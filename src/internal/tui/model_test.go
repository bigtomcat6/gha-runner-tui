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
