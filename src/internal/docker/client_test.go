package docker

import (
	"context"
	"testing"

	"gha-runner-tui/internal/state"
)

type fakeRunner struct {
	outputs map[string][]byte
}

func (f fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return f.outputs[name+" "+joinArgs(args)], nil
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
			"docker ps --all --filter name=gha-remind-me --format {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}": []byte(""),
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
