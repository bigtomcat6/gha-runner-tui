package systemd

import (
	"context"
	"errors"
	"testing"

	"gha-runner-tui/internal/state"
)

type fakeRunner struct {
	outputs map[string][]byte
	errors  map[string]error
}

func (f fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := name + " " + joinArgs(args)
	return f.outputs[key], f.errors[key]
}

func TestStatusParsesActiveAndEnabled(t *testing.T) {
	t.Parallel()

	client := NewClient(fakeRunner{
		outputs: map[string][]byte{
			"systemctl is-active gha-test.service":  []byte("active\n"),
			"systemctl is-enabled gha-test.service": []byte("enabled\n"),
		},
	})

	status, err := client.Status(context.Background(), "gha-test.service")
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Active != state.SystemdActive {
		t.Fatalf("expected active, got %q", status.Active)
	}
	if !status.Enabled {
		t.Fatal("expected enabled service")
	}
}

func TestStatusParsesDisabledServiceFromOutputEvenOnExitError(t *testing.T) {
	t.Parallel()

	client := NewClient(fakeRunner{
		outputs: map[string][]byte{
			"systemctl is-active gha-test.service":  []byte("inactive\n"),
			"systemctl is-enabled gha-test.service": []byte("disabled\n"),
		},
		errors: map[string]error{
			"systemctl is-active gha-test.service":  errors.New("exit status 3"),
			"systemctl is-enabled gha-test.service": errors.New("exit status 1"),
		},
	})

	status, err := client.Status(context.Background(), "gha-test.service")
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if status.Active != state.SystemdInactive {
		t.Fatalf("expected inactive, got %q", status.Active)
	}
	if status.Enabled {
		t.Fatal("expected disabled service")
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
