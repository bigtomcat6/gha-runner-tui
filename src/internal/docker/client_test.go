package docker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/state"
)

type fakeRunner struct {
	outputs map[string][]byte
}

func (f fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f.outputs[name+" "+joinArgs(args)], nil
}

type dockerRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f dockerRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

const pinnedHost = "docker --host unix:///var/run/docker.sock"

const validInspectJSON = `[{"Id":"abc","Name":"/abc","Created":"2024-01-02T03:04:05.123456789Z","Config":{"Image":"img:1","Env":["RUNNER_NAME=rn","RUNNER_TOKEN=secret"],"Labels":{"io.gha-runner-tui.managed":"true"}},"State":{"Status":"running","StartedAt":"2024-01-02T03:04:05Z","ExitCode":7}}]`

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestParseContainerLineNormalizesRunningStatus(t *testing.T) {
	t.Parallel()

	container, err := ParseContainerLine("8f3a12345678\tgha-remind-me-swift-1\tghcr.io/image\tUp 2 minutes")
	if err != nil {
		t.Fatalf("ParseContainerLine returned error: %v", err)
	}
	if container.State != state.ContainerRunning {
		t.Fatalf("expected running, got %q", container.State)
	}
}

func TestCurrentOrLatestReturnsNoneForEmptyOutput(t *testing.T) {
	t.Parallel()

	client := NewClient(fakeRunner{
		outputs: map[string][]byte{
			"docker --host unix:///var/run/docker.sock ps --all --filter name=gha-remind-me --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}": []byte(""),
		},
	})

	container, err := client.CurrentOrLatest(context.Background(), "gha-remind-me")
	if err != nil {
		t.Fatalf("CurrentOrLatest returned error: %v", err)
	}
	if container.State != state.ContainerNone {
		t.Fatalf("expected none, got %q", container.State)
	}
}

func TestNormalizeDockerStatusAcceptsInspectStyleState(t *testing.T) {
	t.Parallel()

	if got := normalizeDockerStatus("running"); got != state.ContainerRunning {
		t.Fatalf("expected running, got %q", got)
	}
	if got := normalizeDockerStatus("exited"); got != state.ContainerExited {
		t.Fatalf("expected exited, got %q", got)
	}
	if got := normalizeDockerStatus("dead"); got != state.ContainerDead {
		t.Fatalf("expected dead, got %q", got)
	}
}

func TestListManagedPreservesTrailingEmptyRunnerLabel(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("id1\trn1\timg:1\trunning\tprofile-a\trn1\nid2\trn2\timg:2\texited\tprofile-a\t\n"), nil
	}))

	containers, err := client.ListManaged(context.Background(), "profile-a")
	if err != nil {
		t.Fatalf("ListManaged returned error: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %+v", containers)
	}
	if containers[0].RunnerName != "rn1" {
		t.Fatalf("expected first runner label, got %+v", containers[0])
	}
	if containers[1].RunnerName != "" {
		t.Fatalf("expected empty trailing runner label, got %+v", containers[1])
	}
	if containers[1].State != state.ContainerExited {
		t.Fatalf("expected exited, got %q", containers[1].State)
	}
}

func TestSlotHoldersExcludesExitedTrailingEmptyRunnerLabel(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("a\tn1\ti\trunning\tp\trn\nb\tn2\ti\texited\tp\t\n"), nil
	}))

	holders, err := client.SlotHolders(context.Background())
	if err != nil {
		t.Fatalf("SlotHolders returned error: %v", err)
	}
	if len(holders) != 1 || holders[0].ID != "a" {
		t.Fatalf("expected only running holder, got %+v", holders)
	}
}

func TestRunPinsHostForEveryDockerMethod(t *testing.T) {
	var calls []string
	runner := dockerRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case hasArg(args, "ps") && hasArg(args, "label=io.gha-runner-tui.managed=true"):
			return []byte("id1\trunner-a\timg:1\trunning\tprofile-a\trn-a\n"), nil
		case hasArg(args, "ps"):
			return []byte("id\tname\timg\tExited (0) 1 minute ago\n"), nil
		case hasArg(args, "inspect"):
			return []byte(validInspectJSON), nil
		case hasArg(args, "wait"):
			return []byte("0\n"), nil
		case hasArg(args, "run"):
			return []byte("cid\n"), nil
		case hasArg(args, "logs"):
			return []byte("log line\n"), nil
		default:
			return []byte(""), nil
		}
	})
	client := NewClient(runner)
	ctx := context.Background()

	if _, err := client.ListByPrefix(ctx, "prefix"); err != nil {
		t.Fatalf("ListByPrefix: %v", err)
	}
	if _, err := client.ListManaged(ctx, "profile-a"); err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if _, err := client.Logs(ctx, "c", 20, false); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if err := client.Kill(ctx, "c"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := client.CleanupExited(ctx, "prefix"); err != nil {
		t.Fatalf("CleanupExited: %v", err)
	}
	if _, err := client.Inspect(ctx, "c"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if _, err := client.RunDetached(ctx, RunSpec{Name: "n", Image: "img"}); err != nil {
		t.Fatalf("RunDetached: %v", err)
	}
	if _, err := client.Wait(ctx, "c"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := client.Remove(ctx, "c", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := client.ReadLogs(ctx, "c"); err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}

	if len(calls) == 0 {
		t.Fatal("expected docker calls")
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, pinnedHost+" ") {
			t.Fatalf("docker call not pinned to host: %q", call)
		}
	}
}

func TestRunIgnoresDockerHostEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///wrong/rootless.sock")

	var calls []string
	runner := dockerRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte(""), nil
	})
	client := NewClient(runner)

	if _, err := client.ListByPrefix(context.Background(), "prefix"); err != nil {
		t.Fatalf("ListByPrefix: %v", err)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], pinnedHost+" ps") {
		t.Fatalf("expected pinned host despite DOCKER_HOST, got %v", calls)
	}
}

func TestSudoRetryPreservesPinnedHostArgs(t *testing.T) {
	var calls []string
	base := dockerRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "docker" {
			return []byte("permission denied"), errors.New("exit status 1")
		}
		return []byte(""), nil
	})
	client := NewClient(command.NewSudoRunner(base))

	if _, err := client.ListByPrefix(context.Background(), "prefix"); err != nil {
		t.Fatalf("ListByPrefix: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected docker + sudo calls, got %v", calls)
	}
	if !strings.HasPrefix(calls[0], pinnedHost+" ps") {
		t.Fatalf("expected pinned docker call, got %q", calls[0])
	}
	if !strings.HasPrefix(calls[1], "sudo -n docker --host unix:///var/run/docker.sock ps") {
		t.Fatalf("sudo retry did not preserve host args: %q", calls[1])
	}
}

func TestListByPrefixReturnsErrorOnNonZeroExitWithStdout(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("id\tname\timg\tUp 1 minute\n"), errors.New("exit status 1")
	}))

	if _, err := client.ListByPrefix(context.Background(), "prefix"); err == nil {
		t.Fatal("expected error even with stdout")
	}
}

func TestListManagedFiltersAndParsesSixColumns(t *testing.T) {
	var args []string
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, got ...string) ([]byte, error) {
		args = got
		return []byte("id1\trunner-a\timg:1\trunning\tprofile-a\trn-a\nid2\trunner-b\timg:2\trestarting\tprofile-b\trn-b\n"), nil
	}))

	containers, err := client.ListManaged(context.Background(), "profile-a")
	if err != nil {
		t.Fatalf("ListManaged returned error: %v", err)
	}
	if !hasArg(args, "label=io.gha-runner-tui.managed=true") {
		t.Fatalf("missing managed label filter: %v", args)
	}
	if !hasArg(args, "label=io.gha-runner-tui.profile=profile-a") {
		t.Fatalf("missing profile label filter: %v", args)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %+v", containers)
	}
	if containers[0].Profile != "profile-a" || containers[0].RunnerName != "rn-a" {
		t.Fatalf("expected decoded labels, got %+v", containers[0])
	}
	if containers[0].State != state.ContainerRunning {
		t.Fatalf("expected running, got %q", containers[0].State)
	}
	if containers[1].State != state.ContainerRestarting {
		t.Fatalf("expected restarting, got %q", containers[1].State)
	}
}

func TestListManagedAllManagedWhenProfileEmpty(t *testing.T) {
	var args []string
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, got ...string) ([]byte, error) {
		args = got
		return []byte(""), nil
	}))

	if _, err := client.ListManaged(context.Background(), ""); err != nil {
		t.Fatalf("ListManaged returned error: %v", err)
	}
	if !hasArg(args, "label=io.gha-runner-tui.managed=true") {
		t.Fatalf("missing managed label filter: %v", args)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "label=io.gha-runner-tui.profile=") {
			t.Fatalf("did not expect profile filter: %v", args)
		}
	}
}

func TestListManagedRejectsMalformedRows(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{name: "too few columns", out: "id\tname\timg\trunning"},
		{name: "empty state", out: "id\tname\timg\t\tprofile\trunner\n"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				return []byte(tt.out), nil
			}))
			if _, err := client.ListManaged(context.Background(), ""); err == nil {
				t.Fatalf("expected parse error for %q", tt.out)
			}
		})
	}
}

func TestListManagedKeepsUnknownState(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("id\tname\timg\tbizarre\tprofile\trunner\n"), nil
	}))

	containers, err := client.ListManaged(context.Background(), "")
	if err != nil {
		t.Fatalf("ListManaged returned error: %v", err)
	}
	if len(containers) != 1 || containers[0].State != state.ContainerUnknown {
		t.Fatalf("expected unknown state, got %+v", containers)
	}
	if !OccupiesSlot(containers[0]) {
		t.Fatal("unknown state must occupy the slot")
	}
}

func TestSlotHoldersFailClosedOnPartialPSOutput(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///wrong/rootless.sock")
	c := NewClient(dockerRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" || len(args) < 3 || args[0] != "--host" || args[1] != "unix:///var/run/docker.sock" {
			t.Fatalf("wrong endpoint: %s %v", name, args)
		}
		return []byte("partial output"), errors.New("exit status 1")
	}))
	if _, err := c.SlotHolders(context.Background()); err == nil {
		t.Fatal("expected ps error")
	}
}

func TestOccupiesSlotIncludesCreatedAndUnknown(t *testing.T) {
	for _, raw := range []string{"created", "running", "restarting", "paused", "unknown"} {
		if !OccupiesSlot(ContainerInfo{State: state.ContainerStatus(raw)}) {
			t.Errorf("must occupy: %s", raw)
		}
	}
	for _, raw := range []string{"exited", "dead", "removing"} {
		if OccupiesSlot(ContainerInfo{State: state.ContainerStatus(raw)}) {
			t.Errorf("must not occupy: %s", raw)
		}
	}
}

func TestSlotHoldersFiltersNonOccupyingStates(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(
			"a\tn1\ti\trunning\tp\tr\n" +
				"b\tn2\ti\texited\tp\tr\n" +
				"c\tn3\ti\tcreated\tp\tr\n" +
				"d\tn4\ti\tdead\tp\tr\n" +
				"e\tn5\ti\tremoving\tp\tr\n" +
				"f\tn6\ti\trestarting\tp\tr\n" +
				"g\tn7\ti\tpaused\tp\tr\n" +
				"h\tn8\ti\tunknown\tp\tr\n"), nil
	}))

	holders, err := client.SlotHolders(context.Background())
	if err != nil {
		t.Fatalf("SlotHolders returned error: %v", err)
	}
	if len(holders) != 5 {
		t.Fatalf("expected 5 slot holders, got %+v", holders)
	}
}

func TestInspectWrapsNotFound(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("Error: No such object: missing"), errors.New("exit status 1")
	}))

	_, err := client.Inspect(context.Background(), "missing")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("expected ErrContainerNotFound, got %v", err)
	}
	if strings.Contains(err.Error(), "No such object") {
		t.Fatalf("raw inspect output leaked: %v", err)
	}
}

func TestInspectFailuresAreNotNotFound(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
		err  error
	}{
		{name: "permission", out: []byte("permission denied"), err: errors.New("exit status 1")},
		{name: "daemon", out: []byte("Cannot connect to the Docker daemon"), err: errors.New("exit status 1")},
		{name: "empty array", out: []byte("[]")},
		{name: "bad json", out: []byte("{not json")},
		{name: "bad created timestamp", out: []byte(`[{"Id":"x","Created":"bad-time","Config":{"Env":[],"Labels":{}},"State":{"Status":"running","StartedAt":"2024-01-02T03:04:05Z","ExitCode":0}}]`)},
		{name: "bad started timestamp", out: []byte(`[{"Id":"x","Created":"2024-01-02T03:04:05Z","Config":{"Env":[],"Labels":{}},"State":{"Status":"running","StartedAt":"bad-time","ExitCode":0}}]`)},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				return tt.out, tt.err
			}))
			_, err := client.Inspect(context.Background(), "c")
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("failure must not be treated as not-found: %v", err)
			}
		})
	}
}

func TestInspectDecodesLabelsAndTimes(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(validInspectJSON), nil
	}))

	details, err := client.Inspect(context.Background(), "c")
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}
	if details.ID != "abc" || details.Name != "abc" || details.Image != "img:1" {
		t.Fatalf("unexpected details: %+v", details)
	}
	if details.State != state.ContainerRunning {
		t.Fatalf("expected running, got %q", details.State)
	}
	if details.Env["RUNNER_NAME"] != "rn" {
		t.Fatalf("expected env decoded, got %+v", details.Env)
	}
	if details.Labels["io.gha-runner-tui.managed"] != "true" {
		t.Fatalf("expected labels decoded, got %+v", details.Labels)
	}
	if details.ExitCode != 7 {
		t.Fatalf("expected exit code 7, got %d", details.ExitCode)
	}
	if details.CreatedAt.IsZero() || details.StartedAt.IsZero() {
		t.Fatalf("expected timestamps decoded, got %+v", details)
	}
}

func TestInspectNormalizesExactContainerStates(t *testing.T) {
	cases := map[string]state.ContainerStatus{
		"created":      state.ContainerCreated,
		"running":      state.ContainerRunning,
		"restarting":   state.ContainerRestarting,
		"paused":       state.ContainerPaused,
		"exited":       state.ContainerExited,
		"dead":         state.ContainerDead,
		"removing":     state.ContainerRemoving,
		"":             state.ContainerNone,
		"exited-ish":   state.ContainerUnknown,
		"deadbeat":     state.ContainerUnknown,
		"createdfoo":   state.ContainerUnknown,
		"up 3 minutes": state.ContainerUnknown,
	}
	for raw, want := range cases {
		t.Run(fmt.Sprintf("status_%q", raw), func(t *testing.T) {
			out := fmt.Sprintf(`[{"Id":"x","Name":"/x","Created":"2024-01-02T03:04:05Z","Config":{"Env":[],"Labels":{}},"State":{"Status":%q,"StartedAt":"2024-01-02T03:04:05Z","ExitCode":0}}]`, raw)
			client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				return []byte(out), nil
			}))

			details, err := client.Inspect(context.Background(), "x")
			if err != nil {
				t.Fatalf("Inspect returned error: %v", err)
			}
			if details.State != want {
				t.Fatalf("status %q: got %q, want %q", raw, details.State, want)
			}
		})
	}
}

func TestRunDetachedAddsSigProxyAndSortedLabels(t *testing.T) {
	var got []string
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		got = append([]string{}, args...)
		return []byte("cid\n"), nil
	}))

	_, err := client.RunDetached(context.Background(), RunSpec{
		Name:  "runner-1",
		Image: "img:1",
		Labels: map[string]string{
			"io.gha-runner-tui.runner":  "rn",
			"io.gha-runner-tui.managed": "true",
			"io.gha-runner-tui.profile": "p",
		},
		Env: map[string]string{"B": "2", "A": "1"},
	})
	if err != nil {
		t.Fatalf("RunDetached returned error: %v", err)
	}

	want := []string{
		"--host", "unix:///var/run/docker.sock",
		"run", "-d", "--sig-proxy=false", "--name", "runner-1",
		"--label", "io.gha-runner-tui.managed=true",
		"--label", "io.gha-runner-tui.profile=p",
		"--label", "io.gha-runner-tui.runner=rn",
		"-e", "A=1",
		"-e", "B=2",
		"img:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunDetached args mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestRunDetachedFailureDoesNotLeakToken(t *testing.T) {
	client := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("failed RUNNER_TOKEN=fake-runner-token"), errors.New("exit status 1")
	}))

	_, err := client.RunDetached(context.Background(), RunSpec{
		Name:  "runner-1",
		Image: "img:1",
		Env:   map[string]string{"RUNNER_TOKEN": "fake-runner-token"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "fake-runner-token") || strings.Contains(err.Error(), "RUNNER_TOKEN") {
		t.Fatalf("raw run error leaked: %v", err)
	}
}

func TestDockerLogsRedactWithoutFullInspect(t *testing.T) {
	for _, readAll := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			calls := 0
			c := NewClient(dockerRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls++
				if name != "docker" || len(args) < 3 || args[0] != "--host" || args[1] != "unix:///var/run/docker.sock" {
					t.Fatalf("endpoint: %s %v", name, args)
				}
				if calls == 1 {
					if !reflect.DeepEqual(args[2:], []string{"inspect", "c"}) {
						t.Fatalf("inspect args: %v", args)
					}
					return []byte(`[{"Created":"bad-time","State":{"Status":17,"StartedAt":"bad-time"},"Config":{"Env":["RUNNER_TOKEN=fake-runner-token","REG_TOKEN=fake-reg-token"]}}]`), nil
				}
				want := []string{"logs", "--tail", "20", "c"}
				if readAll {
					want = []string{"logs", "c"}
				}
				if !reflect.DeepEqual(args[2:], want) {
					t.Fatalf("logs args: %v", args)
				}
				if failed {
					return []byte("fake-runner-token fake-reg-token"), errors.New("failure body fake-runner-token fake-reg-token")
				}
				return []byte("fake-runner-token fake-reg-token"), nil
			}))
			var got string
			var err error
			if readAll {
				got, err = c.ReadLogs(context.Background(), "c")
			} else {
				got, err = c.Logs(context.Background(), "c", 20, false)
			}
			if calls != 2 || got != "[REDACTED] [REDACTED]" || (err != nil) != failed {
				t.Fatalf("calls=%d got=%q err=%v", calls, got, err)
			}
			if err != nil && (strings.Contains(err.Error(), "fake-") || strings.Contains(err.Error(), "failure body")) {
				t.Fatal("raw error leaked")
			}
		}
	}
}

func TestLogsEnvUnavailableFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
		err  error
	}{
		{name: "inspect daemon error", out: []byte("RUNNER_TOKEN=fake-runner-token"), err: errors.New("exit status 1")},
		{name: "env missing", out: []byte(`[{"Config":{}}]`)},
		{name: "env null", out: []byte(`[{"Config":{"Env":null}}]`)},
		{name: "env wrong type", out: []byte(`[{"Config":{"Env":"nope"}}]`)},
		{name: "empty payload", out: []byte(`[]`)},
		{name: "malformed entry", out: []byte(`[{"Config":{"Env":["RUNNER_TOKEN"]}}]`)},
		{name: "bad json", out: []byte(`not json`)},
	}
	for _, tt := range cases {
		for _, readAll := range []bool{false, true} {
			t.Run(tt.name, func(t *testing.T) {
				calls := 0
				c := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
					calls++
					if hasArg(args, "logs") {
						t.Fatal("logs must not run when redaction data is unavailable")
					}
					return tt.out, tt.err
				}))

				var got string
				var err error
				if readAll {
					got, err = c.ReadLogs(context.Background(), "c")
				} else {
					got, err = c.Logs(context.Background(), "c", 20, false)
				}
				if err == nil {
					t.Fatal("expected safe error")
				}
				if got != "" {
					t.Fatalf("expected empty logs, got %q", got)
				}
				if calls != 1 {
					t.Fatalf("expected only inspect call, got %d", calls)
				}
				if strings.Contains(err.Error(), "fake-runner-token") || strings.Contains(err.Error(), "RUNNER_TOKEN") {
					t.Fatalf("raw error leaked: %v", err)
				}
			})
		}
	}
}

func TestLogsAllowsDecodedEnvWithoutTargetTokens(t *testing.T) {
	cases := []struct {
		name string
		out  []byte
	}{
		{name: "empty env", out: []byte(`[{"Config":{"Env":[]}}]`)},
		{name: "other env", out: []byte(`[{"Config":{"Env":["OTHER=x"]}}]`)},
		{name: "empty target values", out: []byte(`[{"Config":{"Env":["RUNNER_TOKEN=","REG_TOKEN="]}}]`)},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					return tt.out, nil
				}
				return []byte("plain logs"), nil
			}))

			got, err := c.Logs(context.Background(), "c", 20, false)
			if err != nil {
				t.Fatalf("Logs returned error: %v", err)
			}
			if got != "plain logs" {
				t.Fatalf("expected plain logs, got %q", got)
			}
		})
	}
}

func TestLogsRedactsSubstringTokens(t *testing.T) {
	calls := 0
	c := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`[{"Config":{"Env":["RUNNER_TOKEN=token","REG_TOKEN=token-long"]}}]`), nil
		}
		return []byte("token-long token"), nil
	}))

	got, err := c.Logs(context.Background(), "c", 20, false)
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	if got != "[REDACTED] [REDACTED]" {
		t.Fatalf("expected full redaction, got %q", got)
	}
}

func TestLogsFollowPreservesArgs(t *testing.T) {
	calls := 0
	var logsArgs []string
	c := NewClient(dockerRunnerFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`[{"Config":{"Env":[]}}]`), nil
		}
		logsArgs = args
		return []byte("x"), nil
	}))

	if _, err := c.Logs(context.Background(), "c", 20, true); err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	want := []string{"--host", "unix:///var/run/docker.sock", "logs", "--tail", "20", "-f", "c"}
	if !reflect.DeepEqual(logsArgs, want) {
		t.Fatalf("logs follow args mismatch: got %v want %v", logsArgs, want)
	}
}

func joinArgs(args []string) string {
	result := ""
	for i, arg := range args {
		if i > 0 {
			result += " "
		}
		result += arg
	}
	return result
}
