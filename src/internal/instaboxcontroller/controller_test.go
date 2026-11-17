package instaboxcontroller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	getFunc      func(context.Context, string) (Machine, error)
	wakeFunc     func(context.Context, string) (Machine, error)
	execFunc     func(context.Context, string, string) (ExecResult, error)
	gets         int
	wakes        int
	execs        int
	wakeIDs      []string
	execIDs      []string
	execCommands []string
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

func (f *fakeAPI) ExecCommand(ctx context.Context, id, command string) (ExecResult, error) {
	f.execs++
	f.execIDs = append(f.execIDs, id)
	f.execCommands = append(f.execCommands, command)
	if f.execFunc == nil {
		return ExecResult{}, errors.New("unexpected exec")
	}
	return f.execFunc(ctx, id, command)
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
	apis := map[string]MachineAPI{}
	for _, machine := range cfg.Machines {
		apis[machine.MachineID] = api
	}
	return newMultiController(cfg, apis, writer, now, log)
}

func newMultiController(cfg Config, apis map[string]MachineAPI, writer DeadlineWriter, now func() time.Time, log *bytes.Buffer) *Controller {
	controller := &Controller{Config: cfg, APIs: apis, Writer: writer, Now: now}
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

func TestPendingReadbackIsNotReadyWithoutPostConfirmationGET(t *testing.T) {
	cfg := testConfig(t, testMachineA)
	cfg.EnableRemoteWrite = true
	now := testNow
	started := testNow
	expected := deadlinePayload(started.Add(120 * time.Minute))
	getCalls := 0
	api := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		getCalls++
		if getCalls >= 3 {
			// A new run may have started while the earlier write was being
			// confirmed. The pending readback alone must not report it ready.
			return runningMachine(testMachineA, started.Add(time.Minute), 120), nil
		}
		return runningMachine(testMachineA, started, 120), nil
	}}
	writer := &fakeWriter{content: map[string]string{}}
	writer.onWrite = func(target RemoteTarget, content string) {
		// The write may have landed even though the answer was lost.
		writer.content[target.key()] = expected
	}
	writer.writeErr = errors.New("connection lost")
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, api, writer, func() time.Time { return now }, logBuffer)
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the unknown write to be reported")
	}
	if writer.writes != 1 || writer.reads != 0 {
		t.Fatalf("first round writes=%d reads=%d", writer.writes, writer.reads)
	}

	writer.writeErr = nil
	writer.onWrite = nil
	logBuffer.Reset()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("a changed run after the pending readback must not be reported ready")
	}
	if writer.reads != 1 || writer.writes != 1 {
		t.Fatalf("matching readback should confirm without rewriting: reads=%d writes=%d", writer.reads, writer.writes)
	}
	if getCalls != 3 {
		t.Fatalf("post-confirmation GET calls = %d, want 3", getCalls)
	}
	if strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("changed run was reported ready: %s", logBuffer.String())
	}
}

func TestExpiredTrustedDeadlineReplacesALooserDeliveredValue(t *testing.T) {
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
	if writer.writes != 1 || writer.lastWrite != deadlinePayload(started.Add(120*time.Minute)) {
		t.Fatalf("first delivery writes=%d value=%q", writer.writes, writer.lastWrite)
	}

	// The platform reports maxRunMinutes 0 after a 120-minute deadline was
	// delivered. The documented meaning is "sleeps immediately", so the new
	// trustworthy deadline is already expired and must replace the looser one.
	now = started.Add(10 * time.Minute)
	maxRun = 0
	logBuffer.Reset()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 2 {
		t.Fatalf("expired trusted deadline was not delivered: writes=%d", writer.writes)
	}
	if want := deadlinePayload(started); writer.lastWrite != want {
		t.Fatalf("delivered = %q, want %q", writer.lastWrite, want)
	}
	if strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("expired run was reported ready: %s", logBuffer.String())
	}
	entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]
	if entry.Status != "confirmed" || entry.Deadline != started.Format(time.RFC3339) {
		t.Fatalf("delivery state = %+v", entry)
	}

	// The conservative value is now confirmed; later rounds must not rewrite.
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 2 {
		t.Fatalf("confirmed expired deadline was rewritten: writes=%d", writer.writes)
	}
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
		{name: "missing required maps", content: `{"version":1}`},
		{name: "missing wake attempts", content: `{"version":1,"deliveries":{}}`},
		{name: "null deliveries", content: `{"version":1,"deliveries":null,"wakeAttempts":{}}`},
		{name: "null wake attempts", content: `{"version":1,"deliveries":{},"wakeAttempts":null}`},
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

func TestLoadStateInitializesOnlyAMissingFile(t *testing.T) {
	missing, err := loadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if missing.Deliveries == nil || missing.WakeAttempts == nil {
		t.Fatalf("a missing state file must initialize valid empty maps: %+v", missing)
	}

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"deliveries":{},"wakeAttempts":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Deliveries == nil || state.WakeAttempts == nil {
		t.Fatalf("legitimate empty maps were lost: %+v", state)
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
	if !strings.Contains(script, "mktemp") || !strings.Contains(script, ".instabox-controller.XXXXXX") || !strings.Contains(script, "mv -f --") {
		t.Fatalf("write script is not an exclusive atomic replacement: %q", script)
	}
	if strings.Contains(script, ".instabox-controller.tmp") {
		t.Fatalf("write script still uses a fixed, symlink-followable temp name: %q", script)
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

	// An api-exec target must never be turned into an SSH invocation.
	apiTarget := RemoteTarget{Delivery: DeliveryAPIExec, MachineID: testMachineA, Path: "/data/x.json"}
	if _, err := sshArgs(apiTarget, "true"); err == nil {
		t.Fatal("an api-exec target was accepted by sshArgs")
	}
	// The api-exec path must still reject unsafe paths and IDs before any request.
	unsafe := RemoteTarget{Delivery: DeliveryAPIExec, MachineID: testMachineA, Path: "/data/../x.json"}
	if _, err := writeScript(unsafe, content); err == nil {
		t.Fatal("unsafe api-exec path got a write script")
	}
	if _, err := writeScript(RemoteTarget{Delivery: DeliveryAPIExec, MachineID: "m_bad", Path: "/data/x.json"}, content); err == nil {
		t.Fatal("unsafe api-exec machine ID got a write script")
	}
}

func TestWriteScriptDoesNotFollowExistingSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "session-deadline.json")
	targetVictim := filepath.Join(dir, "target-victim")
	tmpVictim := filepath.Join(dir, "tmp-victim")
	legacyTmp := target + ".instabox-controller.tmp"
	content := deadlinePayload(testNow.Add(120 * time.Minute))

	if err := os.WriteFile(targetVictim, []byte("keep target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpVictim, []byte("keep tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetVictim, target); err != nil {
		t.Fatal(err)
	}
	// The previous fixed temp name could be a pre-existing symlink, and the
	// shell redirect would truncate the file it pointed at.
	if err := os.Symlink(tmpVictim, legacyTmp); err != nil {
		t.Fatal(err)
	}

	script, err := writeScript(RemoteTarget{
		Host: "runner.example.net",
		User: "dev",
		Port: 22,
		Path: target,
	}, content)
	if err != nil {
		t.Fatal(err)
	}
	// The remote shell body runs locally, confined to this temporary test
	// directory; no SSH or host alias is involved.
	cmd := exec.Command("/bin/sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("write script failed: %v", err)
	}
	if string(out) != content {
		t.Fatalf("readback = %q, want %q", out, content)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != content {
		t.Fatalf("target = %q err=%v", got, err)
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("target must be an atomically replaced regular file: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(targetVictim); err != nil || string(got) != "keep target" {
		t.Fatalf("target symlink target was clobbered: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(tmpVictim); err != nil || string(got) != "keep tmp" {
		t.Fatalf("legacy temp symlink target was clobbered: %q err=%v", got, err)
	}
	if info, err := os.Lstat(legacyTmp); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("a pre-existing unrelated temp path was removed: info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "session-deadline.json", "target-victim", "tmp-victim", filepath.Base(legacyTmp):
		default:
			t.Fatalf("write left an unowned temporary file: %s", entry.Name())
		}
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

func TestMachinesUseTheirOwnAccountKeyForGETAndWake(t *testing.T) {
	type request struct {
		method, path, key string
	}
	var mu sync.Mutex
	var requestsA, requestsB []request
	record := func(requests *[]request) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*requests = append(*requests, request{r.Method, r.URL.Path, r.Header.Get("X-API-Key")})
			mu.Unlock()
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/v1/machines/")
			switch id {
			case testMachineA:
				io.WriteString(w, `{"id":"`+testMachineA+`","state":"running","desiredState":"running","runtime":{"ready":true},"runStartedAt":"`+testNow.Add(-20*time.Minute).Format(time.RFC3339)+`","maxRunMinutes":120}`)
			case testMachineB:
				io.WriteString(w, `{"id":"`+testMachineB+`","state":"suspended","desiredState":"suspended","autoSuspendedAt":"`+testNow.Add(-10*time.Minute).Format(time.RFC3339)+`"}`)
			}
		}
	}
	serverA := httptest.NewServer(record(&requestsA))
	defer serverA.Close()
	serverB := httptest.NewServer(record(&requestsB))
	defer serverB.Close()

	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableWake = true
	cfg.EnableRemoteWrite = true
	apis := map[string]MachineAPI{
		testMachineA: newAPIClient(serverA.URL, "ibk_account_s1", serverA.Client()),
		testMachineB: newAPIClient(serverB.URL, "ibk_account_s2", serverB.Client()),
	}
	controller := newMultiController(cfg, apis, &fakeWriter{}, func() time.Time { return testNow }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	// A is delivered over the fake writer, so its GET, post-write GET and
	// any later check must all stay on the s1 client.
	if len(requestsA) != 2 || requestsA[0].method != http.MethodGet || requestsA[1].method != http.MethodGet {
		t.Fatalf("account s1 requests = %+v", requestsA)
	}
	for _, req := range requestsA {
		if req.path != "/v1/machines/"+testMachineA || req.key != "ibk_account_s1" {
			t.Fatalf("account s1 got %+v", req)
		}
	}
	if len(requestsB) != 2 {
		t.Fatalf("account s2 requests = %+v", requestsB)
	}
	if requestsB[0].method != http.MethodGet || requestsB[0].path != "/v1/machines/"+testMachineB {
		t.Fatalf("first s2 request = %+v", requestsB[0])
	}
	if requestsB[1].method != http.MethodPost || requestsB[1].path != "/v1/machines/"+testMachineB+"/wake" {
		t.Fatalf("wake request = %+v", requestsB[1])
	}
	for _, req := range requestsB {
		if req.key != "ibk_account_s2" {
			t.Fatalf("account s2 got key %q", req.key)
		}
	}
}

func TestControllerWithoutMachineClientFailsBeforeNetworkOrState(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	apiA := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		t.Fatalf("GET for %s happened without its account client", id)
		return Machine{}, nil
	}}
	apis := map[string]MachineAPI{testMachineA: apiA}
	controller := newMultiController(cfg, apis, &fakeWriter{}, func() time.Time { return testNow }, &bytes.Buffer{})
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the missing client to fail the round")
	}
	if apiA.gets != 0 {
		t.Fatalf("gets = %d", apiA.gets)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state was written: %v", err)
	}
}

func TestLoadConfigAcceptsAPIKeyEnvButRejectsInvalidNames(t *testing.T) {
	validMachine := func(id, host, env string) string {
		return fmt.Sprintf(`{"machineId":%q,"apiKeyEnv":%q,"deadlinePath":"/var/lib/gha-runner-tui/state/session-deadline.json","sshHost":%q,"sshUser":"dev"}`, id, env, host)
	}
	wrap := func(machine string) string {
		return `{"stateDir":"/var/lib/instabox-controller","machines":[` + machine + `]}`
	}
	cfg, err := LoadConfig(writeConfigFile(t, wrap(validMachine(testMachineA, "runner.example.net", "INSTABOX_S1_API_KEY"))))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machines[0].APIKeyEnv == nil || *cfg.Machines[0].APIKeyEnv != "INSTABOX_S1_API_KEY" {
		t.Fatalf("apiKeyEnv = %v", cfg.Machines[0].APIKeyEnv)
	}
	for _, env := range []string{"", "S1 KEY", "S1-KEY", "ibk_secret-value", "1S1", "S1\nKEY"} {
		t.Run(env, func(t *testing.T) {
			if _, err := LoadConfig(writeConfigFile(t, wrap(validMachine(testMachineA, "runner.example.net", env)))); err == nil {
				t.Fatal("expected the apiKeyEnv name to be rejected")
			}
		})
	}
}

func TestLegacyStateStillSuppressesWakeWithPerAccountClients(t *testing.T) {
	cfg := testConfig(t, testMachineA, testMachineB)
	cfg.EnableWake = true
	started := testNow
	cycle := started.Add(-10 * time.Minute)
	suspended := suspendedMachine(testMachineB, cycle)
	legacy := `{"version":1,"deliveries":{},"wakeAttempts":{"` + testMachineB + `|` + cycle.Format(time.RFC3339Nano) + `":{"attemptedAt":"` + started.Format(time.RFC3339) + `","status":"sent"}}}`
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	apiA := &fakeAPI{getFunc: func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, started.Add(-20*time.Minute), 120), nil
	}}
	apiB := &fakeAPI{
		getFunc: func(ctx context.Context, id string) (Machine, error) { return suspended, nil },
		wakeFunc: func(ctx context.Context, id string) (Machine, error) {
			t.Fatalf("legacy state did not suppress the wake of %s", id)
			return Machine{}, nil
		},
	}
	apis := map[string]MachineAPI{testMachineA: apiA, testMachineB: apiB}
	controller := newMultiController(cfg, apis, &fakeWriter{}, func() time.Time { return started }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if apiB.wakes != 0 || apiA.gets != 1 || apiB.gets != 1 {
		t.Fatalf("gets A=%d B=%d wakes=%d", apiA.gets, apiB.gets, apiB.wakes)
	}
}

func TestLoadConfigRejectsNullOrNonStringAPIKeyEnv(t *testing.T) {
	wrap := func(machineEnv string) string {
		return `{"stateDir":"/var/lib/instabox-controller","machines":[{"machineId":"` + testMachineA + `","apiKeyEnv":` + machineEnv + `,"deadlinePath":"/var/lib/session-deadline.json","sshHost":"runner.example.net","sshUser":"dev"}]}`
	}
	if _, err := LoadConfig(writeConfigFile(t, wrap(`null`))); err == nil {
		t.Fatal("apiKeyEnv null must be rejected rather than falling back to the global key")
	}
	if _, err := LoadConfig(writeConfigFile(t, wrap(`{"name":"INSTABOX_S1_API_KEY"}`))); err == nil {
		t.Fatal("apiKeyEnv must be a string")
	}
	if _, err := LoadConfig(writeConfigFile(t, wrap(`12345`))); err == nil {
		t.Fatal("apiKeyEnv must be a string, not a number")
	}
	if _, err := LoadConfig(writeConfigFile(t, wrap(`["INSTABOX_S1_API_KEY"]`))); err == nil {
		t.Fatal("apiKeyEnv must be a string, not an array")
	}
}

func TestLoadConfigAcceptsAPIExecWithoutSSHIdentity(t *testing.T) {
	raw := `{"stateDir":"/var/lib/instabox-controller","machines":[{"machineId":"` + testMachineA + `","deliveryTransport":"api-exec","deadlinePath":"/data/gha-runner-tui/session-deadline.json"}]}`
	cfg, err := LoadConfig(writeConfigFile(t, raw))
	if err != nil {
		t.Fatalf("api-exec without SSH fields should be valid: %v", err)
	}
	m := cfg.Machines[0]
	if m.Delivery != DeliveryAPIExec {
		t.Fatalf("delivery = %q", m.Delivery)
	}
	if m.SSHHost != "" || m.SSHUser != "" || m.SSHIdentityFile != "" {
		t.Fatalf("api-exec invented SSH settings: %+v", m)
	}

	withSSH := `{"stateDir":"/var/lib/instabox-controller","machines":[{"machineId":"` + testMachineA + `","deadlinePath":"/data/gha-runner-tui/session-deadline.json","sshHost":"runner.example.net","sshUser":"dev"}]}`
	cfg, err = LoadConfig(writeConfigFile(t, withSSH))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machines[0].Delivery != DeliverySSH {
		t.Fatalf("legacy config did not default to ssh: %q", cfg.Machines[0].Delivery)
	}
	if cfg.Machines[0].SSHPort != 22 {
		t.Fatalf("legacy ssh port = %d", cfg.Machines[0].SSHPort)
	}
}

func TestLoadConfigRejectsUnknownOrNullDeliveryTransport(t *testing.T) {
	wrap := func(value string) string {
		return `{"stateDir":"/var/lib/instabox-controller","machines":[{"machineId":"` + testMachineA + `","deliveryTransport":` + value + `,"deadlinePath":"/data/gha-runner-tui/session-deadline.json"}]}`
	}
	for _, value := range []string{`"telnet"`, `"API-EXEC"`, `""`, `null`, `123`, `{}`, `[]`} {
		t.Run(value, func(t *testing.T) {
			if _, err := LoadConfig(writeConfigFile(t, wrap(value))); err == nil {
				t.Fatalf("deliveryTransport %s was accepted", value)
			}
		})
	}
}

// apiExecFake is a small in-memory remote file plus a fake exec endpoint. It
// answers the same read/write scripts the real adapter sends.
type apiExecFake struct {
	*fakeAPI
	mu      sync.Mutex
	content map[string]string
	readErr error
}

func newAPIExecFake() *apiExecFake {
	f := &apiExecFake{content: map[string]string{}}
	f.fakeAPI = &fakeAPI{execFunc: func(ctx context.Context, id, command string) (ExecResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path, err := quotedPath(command)
		if err != nil {
			return ExecResult{}, err
		}
		if strings.HasPrefix(command, "cat -- ") {
			if f.readErr != nil {
				return ExecResult{}, f.readErr
			}
			value, ok := f.content[path]
			if !ok {
				return ExecResult{ExitCode: 1}, nil
			}
			return ExecResult{Stdout: value, ExitCode: 0}, nil
		}
		f.content[path] = decodeWriteBody(command)
		return ExecResult{Stdout: f.content[path], ExitCode: 0}, nil
	}}
	return f
}

func quotedPath(command string) (string, error) {
	// The scripts carry the deadline path as a leading single-quoted argument.
	for _, marker := range []string{"cat -- '", "mktemp '"} {
		if index := strings.Index(command, marker); index >= 0 {
			rest := command[index+len(marker):]
			end := strings.Index(rest, "'")
			if end >= 0 {
				path := rest[:end]
				if marker == "mktemp '" {
					path = filepath.Dir(path)
				}
				return path, nil
			}
		}
	}
	return "", errors.New("script has no quoted path")
}

func decodeWriteBody(command string) string {
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

func TestAPIExecDeliveryUsesTheMachineAccountAndNoSSHIdentity(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost = ""
	m.SSHUser = ""
	m.SSHPort = 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	cfg.EnableRemoteWrite = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	remote := newAPIExecFake()
	remote.getFunc = func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, now, 120), nil
	}
	sshWriter := &fakeWriter{}
	controller := newController(cfg, remote, sshWriter, func() time.Time { return now }, &bytes.Buffer{})
	defer controller.Close()

	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One write script; the post-confirmation check is a GET, and no SSH
	// writer is used.
	if remote.execs != 1 || sshWriter.writes != 0 || sshWriter.reads != 0 {
		t.Fatalf("execs=%d ssh writes=%d reads=%d", remote.execs, sshWriter.writes, sshWriter.reads)
	}
	if len(remote.execIDs) != 1 || remote.execIDs[0] != testMachineA {
		t.Fatalf("exec machine IDs = %v", remote.execIDs)
	}
	want := deadlinePayload(now.Add(120 * time.Minute))
	if got := remote.content[m.DeadlinePath]; got != want {
		t.Fatalf("remote content = %q, want %q", got, want)
	}
	for _, command := range remote.execCommands {
		if strings.Contains(command, "ibk_") || strings.Contains(command, "X-API-Key") {
			t.Fatalf("the command carried the API key: %q", command)
		}
	}
	if !strings.Contains(remote.execCommands[0], ".instabox-controller.XXXXXX") || !strings.Contains(remote.execCommands[0], "mv -f --") {
		t.Fatalf("write did not reuse the atomic random-temp script: %q", remote.execCommands[0])
	}
	if !strings.Contains(remote.execCommands[0], shellQuote(m.DeadlinePath)) {
		t.Fatalf("write did not shell-quote the configured path: %q", remote.execCommands[0])
	}
	entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]
	if entry.Status != "confirmed" || entry.Target != remoteTarget(m).key() {
		t.Fatalf("delivery state = %+v", entry)
	}

	// A second round with the same run is polling only: no exec at all.
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.execs != 1 {
		t.Fatalf("steady-state round executed %d commands", remote.execs-1)
	}
}

func TestAPIExecEachMachineKeepsItsOwnKeyAndEndpoint(t *testing.T) {
	type request struct {
		path, key, command string
	}
	var mu sync.Mutex
	var requestsA, requestsB []request
	makeServer := func(requests *[]request, machineID string, content string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			var body struct {
				Cmd string `json:"cmd"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			*requests = append(*requests, request{r.URL.Path, r.Header.Get("X-API-Key"), body.Cmd})
			if !strings.HasSuffix(r.URL.Path, "/exec") {
				started := testNow.Format(time.RFC3339)
				maxRun := 120
				id := strings.TrimPrefix(r.URL.Path, "/v1/machines/")
				doc := map[string]any{
					"id": id, "state": "running", "desiredState": "running",
					"runtime":      map[string]any{"ready": true},
					"runStartedAt": started, "maxRunMinutes": maxRun,
				}
				data, _ := json.Marshal(doc)
				io.WriteString(w, string(data))
				return
			}
			if r.URL.Path != "/v1/machines/"+machineID+"/exec" {
				t.Errorf("machine %s exec went to %s", machineID, r.URL.Path)
			}
			// The write script's readback is the deadline payload, so the
			// per-account POSTs are distinguishable but both confirm.
			stdout := content
			if strings.HasPrefix(body.Cmd, "cat -- ") {
				stdout = content
			}
			payload, _ := json.Marshal(map[string]any{
				"stdout": stdout, "stderr": "", "exitCode": 0,
				"timedOut": false, "truncated": false,
			})
			io.WriteString(w, string(payload))
		}))
	}
	serverA := makeServer(&requestsA, testMachineA, deadlinePayload(testNow.Add(120*time.Minute)))
	defer serverA.Close()
	serverB := makeServer(&requestsB, testMachineB, deadlinePayload(testNow.Add(120*time.Minute)))
	defer serverB.Close()

	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	for _, id := range []string{testMachineA, testMachineB} {
		m := testMachineConfig(id)
		m.Delivery = DeliveryAPIExec
		m.SSHHost = ""
		m.SSHUser = ""
		m.SSHPort = 0
		cfg.Machines = append(cfg.Machines, m)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	apis := map[string]MachineAPI{
		testMachineA: newAPIClient(serverA.URL, "ibk_account_s1", serverA.Client()),
		testMachineB: newAPIClient(serverB.URL, "ibk_account_s2", serverB.Client()),
	}
	controller := newMultiController(cfg, apis, &fakeWriter{}, func() time.Time { return testNow }, &bytes.Buffer{})
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, tc := range []struct {
		name     string
		requests []request
		key      string
		path     string
	}{
		{name: "s1", requests: requestsA, key: "ibk_account_s1", path: "/v1/machines/" + testMachineA},
		{name: "s2", requests: requestsB, key: "ibk_account_s2", path: "/v1/machines/" + testMachineB},
	} {
		// Initial GET, one exec write, and the post-confirmation GET.
		if len(tc.requests) != 3 {
			t.Fatalf("%s requests = %+v", tc.name, tc.requests)
		}
		if tc.requests[0].path != tc.path || tc.requests[1].path != tc.path+"/exec" || tc.requests[2].path != tc.path {
			t.Fatalf("%s request paths = %+v", tc.name, tc.requests)
		}
		for _, req := range tc.requests {
			if req.key != tc.key {
				t.Fatalf("%s used key %q", tc.name, req.key)
			}
			if strings.Contains(req.command, "ibk_") || strings.Contains(req.command, "X-API-Key") {
				t.Fatalf("%s command carried the key", tc.name)
			}
		}
	}
}

func TestAPIExecStillRequiresEnableAndDryRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
		dryRun bool
	}{
		{name: "default off"},
		{name: "dry run", enable: true, dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: tc.enable}
			m := testMachineConfig(testMachineA)
			m.Delivery = DeliveryAPIExec
			m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
			cfg.Machines = []MachineConfig{m}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			now := testNow
			api := newAPIExecFake()
			api.getFunc = func(ctx context.Context, id string) (Machine, error) {
				return runningMachine(testMachineA, now, 120), nil
			}
			sshWriter := &fakeWriter{}
			controller := newController(cfg, api, sshWriter, func() time.Time { return now }, &bytes.Buffer{})
			controller.DryRun = tc.dryRun
			defer controller.Close()
			if err := controller.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if api.execs != 0 || sshWriter.writes != 0 || sshWriter.reads != 0 {
				t.Fatalf("execs=%d ssh writes=%d reads=%d, want none", api.execs, sshWriter.writes, sshWriter.reads)
			}
		})
	}
}

func TestAPIExecUnknownWriteIsOnlyResolvedAfterReadback(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	expected := deadlinePayload(now.Add(120 * time.Minute))
	remote := newAPIExecFake()
	remote.getFunc = func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, now, 120), nil
	}
	// The first write lands but the answer is lost.
	realExec := remote.fakeAPI.execFunc
	var lostOnce sync.Once
	remote.fakeAPI.execFunc = func(ctx context.Context, id, command string) (ExecResult, error) {
		result, err := realExec(ctx, id, command)
		if err == nil && strings.HasPrefix(command, "umask 077") {
			var skip bool
			lostOnce.Do(func() { skip = true })
			if skip {
				return ExecResult{}, errors.New("instabox API request failed")
			}
		}
		return result, err
	}

	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, remote, &fakeWriter{}, func() time.Time { return now }, logBuffer)
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the unknown write to be reported")
	}
	if remote.execs != 1 || !strings.Contains(logBuffer.String(), "budget-ready=false") {
		t.Fatalf("execs=%d log=%s", remote.execs, logBuffer.String())
	}
	if entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]; entry.Status != "pending" {
		t.Fatalf("state = %+v", entry)
	}

	// Next round reads first; identical content confirms without a rewrite,
	// and the post-confirmation GET still runs.
	logBuffer.Reset()
	if err := controller.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remote.content[m.DeadlinePath]; got != expected {
		t.Fatalf("content = %q", got)
	}
	if remote.execs != 2 {
		t.Fatalf("execs = %d, want one readback and no second write (1 lost write + 1 read)", remote.execs)
	}
	if !strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("matching readback was not confirmed: %s", logBuffer.String())
	}
	if calls := remote.gets; calls != 3 {
		t.Fatalf("GET calls = %d, want initial + post-confirmation", calls)
	}
}

func seedPendingDelivery(t *testing.T, controller *Controller, m MachineConfig, now time.Time) {
	t.Helper()
	if err := controller.ensureReady(); err != nil {
		t.Fatal(err)
	}
	controller.state.Deliveries[m.MachineID] = deliveryState{
		Target:       remoteTarget(m).key(),
		RunStartedAt: now.Format(time.RFC3339Nano),
		Deadline:     now.Add(120 * time.Minute).Format(time.RFC3339),
		Status:       "pending",
		AttemptedAt:  now,
	}
	if err := controller.saveState(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIExecChangedRunAfterReadbackIsNotReady(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	expected := deadlinePayload(now.Add(120 * time.Minute))
	remote := newAPIExecFake()
	remote.content[m.DeadlinePath] = expected
	getCalls := 0
	remote.getFunc = func(ctx context.Context, id string) (Machine, error) {
		getCalls++
		if getCalls >= 2 {
			// A new run started while the readback was confirming the old one.
			return runningMachine(testMachineA, now.Add(time.Minute), 120), nil
		}
		return runningMachine(testMachineA, now, 120), nil
	}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, remote, &fakeWriter{}, func() time.Time { return now }, logBuffer)
	defer controller.Close()
	seedPendingDelivery(t, controller, m, now)

	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("a changed run after the confirmation readback must be reported")
	}
	if remote.execs != 1 {
		t.Fatalf("execs = %d, want only the readback", remote.execs)
	}
	if getCalls != 2 {
		t.Fatalf("GET calls = %d, want initial + post-confirmation", getCalls)
	}
	if strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("changed run reported ready: %s", logBuffer.String())
	}
}

func TestAPIExecReadbackFailureKeepsPendingAndDoesNotRewrite(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	expected := deadlinePayload(now.Add(120 * time.Minute))
	remote := newAPIExecFake()
	remote.content[m.DeadlinePath] = expected
	remote.readErr = errors.New("instabox API request failed")
	remote.getFunc = func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, now, 120), nil
	}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, remote, &fakeWriter{}, func() time.Time { return now }, logBuffer)
	defer controller.Close()
	seedPendingDelivery(t, controller, m, now)

	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the unconfirmed readback to be reported")
	}
	// Only the read was attempted: no blind rewrite after the failed read.
	if remote.execs != 1 || !strings.HasPrefix(remote.execCommands[0], "cat -- ") {
		t.Fatalf("execs=%d commands=%v", remote.execs, remote.execCommands)
	}
	if entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]; entry.Status != "pending" {
		t.Fatalf("read failure changed state: %+v", entry)
	}
	if strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("read failure reported ready: %s", logBuffer.String())
	}
}

func TestAPIExecNonzeroExitIsNeverAConfirmedReadback(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	remote := newAPIExecFake()
	remote.getFunc = func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, now, 120), nil
	}
	// The readback's cat fails and prints nothing.
	remote.execFunc = func(ctx context.Context, id, command string) (ExecResult, error) {
		return ExecResult{ExitCode: 1}, nil
	}
	remote.readErr = nil
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, remote, &fakeWriter{}, func() time.Time { return now }, logBuffer)
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed write must be reported")
	}
	if strings.Contains(logBuffer.String(), "budget-ready=true") {
		t.Fatalf("nonzero exit reported ready: %s", logBuffer.String())
	}
	if entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]; entry.Status != "pending" {
		t.Fatalf("state = %+v", entry)
	}
}

func TestAPIExecPermissionDeniedStaysBlockedWithoutSudoOrLeak(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	remote := newAPIExecFake()
	remote.getFunc = func(ctx context.Context, id string) (Machine, error) {
		return runningMachine(testMachineA, now, 120), nil
	}
	// The login user cannot create the temp file in the target directory.
	remote.execFunc = func(ctx context.Context, id, command string) (ExecResult, error) {
		return ExecResult{Stderr: "mktemp: Permission denied " + m.DeadlinePath, ExitCode: 1}, nil
	}
	logBuffer := &bytes.Buffer{}
	controller := newController(cfg, remote, &fakeWriter{}, func() time.Time { return now }, logBuffer)
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("a blocked write must be reported")
	}
	logs := logBuffer.String()
	if !strings.Contains(logs, "did not complete successfully") {
		t.Fatalf("logs = %s", logs)
	}
	// stderr is not copied into logs, and nothing escalates to sudo.
	if strings.Contains(logs, "Permission denied") {
		t.Fatalf("remote stderr was logged: %s", logs)
	}
	for _, command := range remote.execCommands {
		if strings.Contains(command, "sudo") || strings.Contains(command, "root") {
			t.Fatalf("command escalated privileges: %q", command)
		}
	}
	if entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]; entry.Status != "pending" {
		t.Fatalf("blocked write must stay unconfirmed: %+v", entry)
	}
	if strings.Contains(logs, "budget-ready=true") {
		t.Fatalf("blocked write reported ready: %s", logs)
	}
}

func TestAPIExec403FailsWithoutFallbackToSSH(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), EnableRemoteWrite: true}
	m := testMachineConfig(testMachineA)
	m.Delivery = DeliveryAPIExec
	m.SSHHost, m.SSHUser, m.SSHPort = "", "", 0
	m.DeadlinePath = "/data/gha-runner-tui/session-deadline.json"
	cfg.Machines = []MachineConfig{m}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	now := testNow
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"missing_scope","message":"ibk_secret_value","requiredScopes":["machines:write"]}}`)
			return
		}
		started := testNow.Format(time.RFC3339)
		maxRun := 120
		io.WriteString(w, `{"id":"`+testMachineA+`","state":"running","desiredState":"running","runtime":{"ready":true},"runStartedAt":"`+started+`","maxRunMinutes":`+fmt.Sprint(maxRun)+`}`)
	}))
	defer server.Close()
	sshWriter := &fakeWriter{}
	logBuffer := &bytes.Buffer{}
	controller := newMultiController(cfg, map[string]MachineAPI{
		testMachineA: newAPIClient(server.URL, "ibk_secret_value", server.Client()),
	}, sshWriter, func() time.Time { return now }, logBuffer)
	defer controller.Close()
	if err := controller.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the refused exec to be reported")
	}
	if sshWriter.writes != 0 || sshWriter.reads != 0 {
		t.Fatalf("403 fell back to SSH: writes=%d reads=%d", sshWriter.writes, sshWriter.reads)
	}
	if strings.Contains(logBuffer.String(), "ibk_secret_value") {
		t.Fatalf("logs leaked the key: %s", logBuffer.String())
	}
	if !strings.Contains(logBuffer.String(), "missing_scope") {
		t.Fatalf("logs = %s", logBuffer.String())
	}
	if entry := readStateFile(t, cfg.StateDir).Deliveries[testMachineA]; entry.Status != "pending" {
		t.Fatalf("refused write must stay unconfirmed: %+v", entry)
	}
}
