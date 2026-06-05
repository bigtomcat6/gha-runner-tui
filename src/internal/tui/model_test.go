package tui

import (
	"testing"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/config"
)

func TestReadCreateInputParsesOrganizationMode(t *testing.T) {
	t.Parallel()

	m := NewModel(testManager())
	m.createFields = newCreateFields()
	setCreateField(&m, "Target scope", "organization")
	setCreateField(&m, "Organization", "Example Org")
	setCreateField(&m, "Environment", "swift")
	setCreateField(&m, "Docker access", "rootless")
	setCreateField(&m, "Runner labels", "self-hosted,linux,x64,docker,swift")
	setCreateField(&m, "Docker image", "gha-runner-swift:latest")
	setCreateField(&m, "CPU limit", "2")
	setCreateField(&m, "Memory limit", "4g")
	setCreateField(&m, "Ephemeral", "true")

	input, err := m.readCreateInput()
	if err != nil {
		t.Fatalf("readCreateInput returned error: %v", err)
	}
	if input.Scope != config.TargetScopeOrganization {
		t.Fatalf("expected organization scope, got %q", input.Scope)
	}
	if input.Org != "Example Org" || input.Environment != "swift" {
		t.Fatalf("unexpected org input: %+v", input)
	}
	if input.DockerAccess != "rootless" {
		t.Fatalf("expected rootless docker access, got %q", input.DockerAccess)
	}
}

func TestReadCreateInputParsesDockerAccess(t *testing.T) {
	t.Parallel()

	m := NewModel(testManager())
	m.createFields = newCreateFields()
	setCreateField(&m, "Docker access", "host-socket")
	setCreateField(&m, "Profile name", "remind-me-swift")
	setCreateField(&m, "Repo owner", "bigtomcat6")
	setCreateField(&m, "Repo name", "remind-me")
	setCreateField(&m, "Docker image", "gha-runner-base:latest")
	setCreateField(&m, "Service name", "gha-remind-me-swift.service")
	setCreateField(&m, "Container prefix", "gha-remind-me-swift")

	input, err := m.readCreateInput()
	if err != nil {
		t.Fatalf("readCreateInput returned error: %v", err)
	}
	if input.DockerAccess != "host-socket" {
		t.Fatalf("expected host-socket docker access, got %q", input.DockerAccess)
	}
}

func TestReadCreateInputRejectsInvalidDockerAccess(t *testing.T) {
	t.Parallel()

	m := NewModel(testManager())
	m.createFields = newCreateFields()
	setCreateField(&m, "Docker access", "invalid")
	setCreateField(&m, "Profile name", "remind-me-swift")
	setCreateField(&m, "Repo owner", "bigtomcat6")
	setCreateField(&m, "Repo name", "remind-me")
	setCreateField(&m, "Docker image", "gha-runner-base:latest")
	setCreateField(&m, "Service name", "gha-remind-me-swift.service")
	setCreateField(&m, "Container prefix", "gha-remind-me-swift")

	_, err := m.readCreateInput()
	if err == nil {
		t.Fatal("expected invalid docker access error, got nil")
	}
}

func testManager() app.RunnerManager {
	return app.RunnerManager{}
}

func setCreateField(m *Model, label, value string) {
	for i := range m.createFields {
		if m.createFields[i].label == label {
			m.createFields[i].input.SetValue(value)
			return
		}
	}
}
